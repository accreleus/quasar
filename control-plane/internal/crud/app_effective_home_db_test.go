package crud

import (
	"net/http"
	"testing"
)

// effective_managed_home / effective_home_container_path (control-api.md
// amendment 82, accreleus/quasar#1201): the admin app shape reports the home a
// launch of the app gets NOW, by the launch path's own merge rule, so the editor
// can show what will really happen instead of the app's stored columns alone.
// The derived tile is the case that makes the root lookup load-bearing: a tile
// carries no managed home and no preset of its own, and takes both from its
// parent.
func TestAdminAppEffectiveManagedHome(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	tok := adminToken(t, pool, authSvc)

	preset := createHomePreset(t, srv.URL, tok, map[string]any{
		"name":                "homes-on",
		"managed_home":        true,
		"home_container_path": "/srv/homes",
	})
	plainPreset := createHomePreset(t, srv.URL, tok, map[string]any{"name": "homes-off"})

	// The preset provides the managed home; the app states neither, so its own
	// columns keep the schema defaults and the effective pair follows the preset.
	parent := createApp(t, srv.URL, tok, map[string]any{
		"name":              "parent-app",
		"runtime_preset_id": preset,
	})
	assertEffective(t, "create answer", parent, true, "/srv/homes")
	if parent["managed_home"] != false || parent["home_container_path"] != "/home/quasar" {
		t.Fatalf("stored columns = (%v, %v), want the app's own (false, /home/quasar) — "+
			"the stored values keep their meaning", parent["managed_home"], parent["home_container_path"])
	}

	// The editor's load path (admin branch of GET /v1/apps/{id}) agrees.
	resp, body := getReq(t, srv.URL+"/v1/apps/"+parent["id"].(string), tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get app: want 200, got %d", resp.StatusCode)
	}
	assertEffective(t, "GET /v1/apps/{id} (admin)", body["app"].(map[string]any), true, "/srv/homes")

	// ... and so does the admin catalogue.
	resp, body = getReq(t, srv.URL+"/v1/admin/apps", tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list admin apps: want 200, got %d", resp.StatusCode)
	}
	for _, raw := range body["items"].([]any) {
		item := raw.(map[string]any)
		if item["id"] == parent["id"] {
			assertEffective(t, "GET /v1/admin/apps", item, true, "/srv/homes")
		}
	}

	// A derived tile: no managed home, no preset, the schema default path — and
	// still the parent's effective home, because that is what its launch binds.
	tile := createApp(t, srv.URL, tok, map[string]any{
		"name":            "tile-app",
		"parent_app_id":   parent["id"],
		"external_source": "steam",
		"external_id":     "440",
	})
	assertEffective(t, "derived tile create", tile, true, "/srv/homes")
	if tile["managed_home"] != false || tile["runtime_preset_id"] != nil {
		t.Fatalf("tile's own columns = (%v, %v), want (false, null)", tile["managed_home"], tile["runtime_preset_id"])
	}
	resp, body = getReq(t, srv.URL+"/v1/apps/"+tile["id"].(string), tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get tile: want 200, got %d", resp.StatusCode)
	}
	assertEffective(t, "derived tile GET", body["app"].(map[string]any), true, "/srv/homes")

	// A preset that provides no home leaves the app's own choice alone; the path
	// is the app's, because it is not the schema default.
	own := createApp(t, srv.URL, tok, map[string]any{
		"name":                "own-home-app",
		"managed_home":        true,
		"home_container_path": "/data/home",
		"runtime_preset_id":   plainPreset,
	})
	assertEffective(t, "preset without a home", own, true, "/data/home")

	// And with no preset at all the effective pair is the stored pair.
	noPreset := createApp(t, srv.URL, tok, map[string]any{"name": "no-preset-app"})
	assertEffective(t, "no preset", noPreset, false, "/home/quasar")
}

func assertEffective(t *testing.T, where string, app map[string]any, managed bool, path string) {
	t.Helper()
	if _, ok := app["effective_managed_home"]; !ok {
		t.Fatalf("%s: effective_managed_home absent — an amendment-aware server always serializes it", where)
	}
	if _, ok := app["effective_home_container_path"]; !ok {
		t.Fatalf("%s: effective_home_container_path absent — an amendment-aware server always serializes it", where)
	}
	if app["effective_managed_home"] != managed || app["effective_home_container_path"] != path {
		t.Fatalf("%s: effective home = (%v, %v), want (%v, %q)",
			where, app["effective_managed_home"], app["effective_home_container_path"], managed, path)
	}
}

func createHomePreset(t *testing.T, base, tok string, body map[string]any) string {
	t.Helper()
	resp, parsed := post(t, base+"/v1/admin/runtime-presets", body, tok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create preset %v: want 201, got %d (%v)", body["name"], resp.StatusCode, parsed)
	}
	return parsed["runtime_preset"].(map[string]any)["id"].(string)
}

func createApp(t *testing.T, base, tok string, body map[string]any) map[string]any {
	t.Helper()
	resp, parsed := post(t, base+"/v1/apps", body, tok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app %v: want 201, got %d (%v)", body["name"], resp.StatusCode, parsed)
	}
	return parsed["app"].(map[string]any)
}
